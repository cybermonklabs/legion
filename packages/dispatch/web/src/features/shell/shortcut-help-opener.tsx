import { createContext, useContext } from "react";

/**
 * Opens the list of the keys the page binds (`ShortcutHelp.tsx`), which `?` opens, for a page
 * that draws a control of its own for it: the issue page's Keys, at the right of its filter row
 * (dispatch://INFRA-1196 R11 and the 1440 px picture). `undefined` outside the shell, where no
 * list is drawn to open.
 */
export const ShortcutHelpOpener = createContext<(() => void) | undefined>(undefined);

export function useShortcutHelpOpener(): (() => void) | undefined {
  return useContext(ShortcutHelpOpener);
}
