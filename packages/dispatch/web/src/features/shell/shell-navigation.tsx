import { createContext, useContext } from "react";

/**
 * The shell's navigation, for a page that draws the control opening it: on the issue page below
 * the phone's width the shell draws no bar of its own, and the issue's header carries the `☰`
 * that opens the navigation over the page (dispatch://INFRA-1196 R14, the 390 px picture).
 * `undefined` wherever the shell draws that control itself, and outside the shell.
 */
export interface ShellNavigation {
  readonly open: boolean;
  readonly onOpen: () => void;
}

export const ShellNavigationContext = createContext<ShellNavigation | undefined>(undefined);

export function useShellNavigation(): ShellNavigation | undefined {
  return useContext(ShellNavigationContext);
}
