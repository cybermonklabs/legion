import type { ReactNode } from "react";

/**
 * The control that comments on selected words (R5): while a selection lies inside the words of
 * one row it draws a button beside the selection's end (`position: fixed`, `left` and `top` in
 * px from the last client rectangle of the range), and `$mod+Alt+m` (matched by
 * `event.code === "KeyM"`) does what the button does. A press, or the chord with a quote, opens
 * the composer (`useTextComposer`) and closes Details first. It draws no control on an
 * author-only node, a child issue's row, or a kind whose gate is off (`text-comment-write.ts`).
 * It takes no property: it reads the nodes the stage draws from the provider that
 * `slots/providers.tsx` mounts. The stage mounts it only where `GATES.COMPOSER` is on. Built by
 * t8; it draws nothing until then.
 */
export function SelectionComment(): ReactNode {
  return null;
}
