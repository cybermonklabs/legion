import { createContext, type RefCallback, useContext } from "react";

/**
 * The shell's navigation, for a page that draws the control opening it: on the issue page below
 * the phone's width the shell draws no bar of its own, and the issue's header carries the `☰`
 * that opens the navigation over the page (R14, the 390 px picture).
 * `undefined` wherever the shell draws that control itself, and outside the shell.
 */
export interface ShellNavigation {
  readonly open: boolean;
  readonly onOpen: () => void;
  /** The row that holds Menu. The shell measures its bottom edge for the widened command bar. */
  readonly topBarRef?: RefCallback<HTMLDivElement>;
}

export const ShellNavigationContext = createContext<ShellNavigation | undefined>(undefined);

export function useShellNavigation(): ShellNavigation | undefined {
  return useContext(ShellNavigationContext);
}
