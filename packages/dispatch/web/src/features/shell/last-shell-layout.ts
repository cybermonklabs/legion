/**
 * The last shell layout this browser drew, under one key for the whole browser.
 *
 * The shell's own preferences are private to the signed-in identity, so their keys carry the
 * login (`features/shell/userPreference.ts`) and the page can read none of them until
 * `/auth/whoami` has answered - which is far too late for the blocks the server drew into the
 * page, already on the screen before any script ran (`web/index.html`,
 * `internal/dispatch/routes/first_screen.go`). Those blocks were laid out in the default
 * columns and re-wrapped once the reader's own came back: measured at 1280x900, 528 px to
 * 392 px for a margin stored at 520 px, 528 px to 856 px for a margin collapsed to its rail,
 * and 528 px to 792 px for a sidebar hidden to its rail (dispatch://INFRA-341, R11).
 *
 * So the layout is written here as well, under a key that names no login, and the head of
 * `web/index.html` reads it before `#first-screen` is parsed and sets the two properties
 * `web/src/styles.css` lays those blocks out by. The login-keyed preferences stay the source of
 * truth the moment the app runs; this is only what the first paint can read, and it holds no
 * login, no document and nothing a reader would mind the next person at the machine seeing -
 * two column widths.
 *
 * One key rather than one per column: two keys can be written in two loads and disagree, and
 * the first paint has to lay the blocks out from both at once.
 *
 * For one reader per browser, which is the ordinary case, the re-wrap is gone. Where two logins
 * share a browser the key holds whichever of them drew last, so the other sees one re-wrap on
 * their first paint until the shell writes their own layout, which it does on that same load.
 */

/** Where the layout is kept. No login in it: the first paint does not know one yet. */
export const LAST_SHELL_LAYOUT_KEY = "dispatch.shell.layout";

/** The custom properties the head's script sets and `styles.css` reads: how much room the
 *  server's draw leaves on each side, `<main>`'s own padding included. */
export const FIRST_SCREEN_SIDEBAR_PROPERTY = "--first-screen-sidebar-inset";
export const FIRST_SCREEN_MARGIN_PROPERTY = "--first-screen-margin-inset";

/**
 * What is written: the margin's half, then the sidebar's, separated by `/`.
 *
 * The margin's half is `rail` for a margin collapsed to its rail and the column's width in
 * pixels otherwise; the sidebar's is `rail` or `shown`. A collapsed column draws no column at
 * all and the shell pads the gutter for the rail instead, so a rail and a width leave different
 * room and cannot be one number.
 */
export function shellLayoutValue(
  marginCollapsed: boolean,
  marginWidth: number,
  sidebarHidden: boolean
): string {
  return `${marginCollapsed ? "rail" : marginWidth}/${sidebarHidden ? "rail" : "shown"}`;
}

/** Writes the layout the shell is drawing. Called whenever any of its three parts changes, so
 *  the key holds what this browser last drew rather than what it was last asked to draw.
 *  Storage that cannot be written loses nothing: the rule in `styles.css` carries the
 *  defaults. */
export function rememberShellLayout(
  marginCollapsed: boolean,
  marginWidth: number,
  sidebarHidden: boolean
): void {
  try {
    window.localStorage.setItem(
      LAST_SHELL_LAYOUT_KEY,
      shellLayoutValue(marginCollapsed, marginWidth, sidebarHidden)
    );
  } catch {
    return;
  }
}
