// Rewrite and Define (INFRA-3231's R7): what a read carries of a rewrite - the `rewrite` every
// read of an ask, a comment, a message and an issue declares in `@legion/contracts` (Decision 23)
// - which wording a page shows of a rewritten text, and where the words a reader defines stand.
// The routes themselves are the typed client's (`api.rewriteAsk`, `api.defineWords`,
// `api.getDefinition`).
import { type KeyboardEvent, useSyncExternalStore } from "react";

import type { TextRewrite, TextWording } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";

/**
 * The wording a rewritten text shows while its badge shows the original: the read's `original`,
 * where the rewrite is in place (`rewritten`) and the reader asked for it (`shown`). A rewrite
 * that runs or failed left the text as it was, so it has no other wording to show.
 */
export function originalWording(
  rewrite: Pick<TextRewrite, "original" | "state"> | undefined,
  shown: boolean
): TextWording | undefined {
  return shown && rewrite?.state === "rewritten" ? rewrite.original : undefined;
}

/** The newer of two readings of one text's rewrite, by when each was started, written or failed
 *  (`at`): a press's own answer and the read that is refreshed behind it. Of two at one instant,
 *  the read's is the later reading. */
export function newerRewrite(
  answer: TextRewrite | undefined,
  read: TextRewrite | undefined
): TextRewrite | undefined {
  if (answer === undefined) return read;
  if (read === undefined) return answer;
  return compareTimestamps(read.at, answer.at) >= 0 ? read : answer;
}

/** Which rewritten texts show their original wording now, by the text's id (a comment's id, a
 *  message's, an issue's key): one place, so every part of a page that draws the text - an
 *  issue's title in its header and its spec in the tab under it - swaps together. */
const originalsShown = new Set<string>();
const originalListeners = new Set<() => void>();

export function toggleOriginal(id: string): void {
  if (!originalsShown.delete(id)) originalsShown.add(id);
  for (const listener of originalListeners) listener();
}

/** Whether the text `id` shows its original wording now. */
export function useOriginalShown(id: string): boolean {
  return useSyncExternalStore(
    (listener) => {
      originalListeners.add(listener);
      return () => originalListeners.delete(listener);
    },
    () => originalsShown.has(id)
  );
}

/** Whether `event`, pressed on a rewritten text that holds the focus, is the `w` that shows or
 *  hides its original wording: no modifier, and not typed into a field inside the text. */
function isOriginalToggleKey(event: KeyboardEvent<HTMLElement>): boolean {
  return (
    event.key === "w" &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.altKey &&
    !(
      event.target instanceof HTMLElement &&
      (event.target.isContentEditable || event.target.closest("input, textarea") !== null)
    )
  );
}

/** What the element holding a text spreads so that `w` shows or hides the text's original
 *  wording (`originalToggleProps`). */
export interface OriginalToggleProps {
  readonly onKeyDown: (event: KeyboardEvent<HTMLElement>) => void;
  readonly tabIndex: -1 | undefined;
}

/**
 * The props of the element that holds the text `id`, a margin card or a message, which make `w`
 * show or hide its original wording while `rewritten`: the element takes the focus a press on
 * the text gives it, and its own `w` toggles the text (`isOriginalToggleKey`). A text that is not
 * rewritten takes neither. A `w` a text drawn inside the element already took - a rewritten
 * reply inside its thread's card - is that text's alone, so the element leaves it.
 */
export function originalToggleProps(id: string, rewritten: boolean): OriginalToggleProps {
  return {
    onKeyDown: (event) => {
      if (!rewritten || event.defaultPrevented || !isOriginalToggleKey(event)) return;
      event.preventDefault();
      toggleOriginal(id);
    },
    tabIndex: rewritten ? -1 : undefined,
  };
}

/** Where the words a reader selected stand, as `POST /api/v1/definitions` names it: in an ask,
 *  a decision block's included (`ask_id`), or in a document of an issue or a project, by its id
 *  (`document`), which Dispatch reads at its latest version. */
export type DefinitionPlace = { readonly ask_id: string } | { readonly document: string };
