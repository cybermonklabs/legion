/**
 * The class strings the entry chunk's modules draw: the shell every page draws first - the rail,
 * the status corner, the page's frame - and the texts, borders, buttons, fields and callouts they
 * share, with every string one of them is built from. Every other class string of the theme, and
 * every OKLCH pair with its contrast check (`CONTRAST_CHECKS`), is in `classes.ts`: that module
 * builds its pairs and registers its checks as it loads, so any chunk that imports it carries all
 * of them, and the entry imports only this one. A string here is still a static literal for
 * Tailwind's scanner, under the rules `classes.ts` states, and its pair and check are in that
 * module under the role's name. A string moves here when a module of the entry comes to draw it;
 * every importer reads the module that holds its names.
 */

export const surfaceBg = "bg-white dark:bg-slate-900";
export const surfaceMutedBg = "bg-slate-100 dark:bg-slate-800";
/** An `animate-pulse` skeleton placeholder: `SURFACE_MUTED`'s shades reused directly (both
 * `bg-slate-100` and `bg-slate-200` skeletons in the app already resolve to the same
 * `dark:bg-slate-800`; this consolidates the light side to one shade). */
export const skeletonBg = surfaceMutedBg;
export const textPrimaryOnCanvas = "text-slate-900 dark:text-slate-100";
export const textSecondaryOnCanvas = "text-slate-700 dark:text-slate-300";
export const textSecondaryOnSurface = "text-slate-700 dark:text-slate-300";
export const textMutedOnSurface = "text-slate-500 dark:text-slate-400";
export const textMutedOnSurfaceMuted = "text-slate-600 dark:text-slate-400";
/** A dialog/composer "Close" button: secondary at rest, primary-emphasis on hover. Not
 * registered separately — both ends reuse `TEXT_SECONDARY`/`TEXT_PRIMARY`'s own checks. */
export const dismissButtonText =
  "text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100";
/** A disclosure chevron: disabled-shade at rest, secondary-emphasis on hover. */
export const disclosureButtonText =
  "text-slate-400 dark:text-slate-600 hover:text-slate-700 dark:hover:text-slate-300";
/** A "dismiss"/cancel affordance already showing muted text: secondary-emphasis on hover. */
export const textMutedHoverToSecondary =
  "text-slate-500 dark:text-slate-400 hover:text-slate-700 dark:hover:text-slate-300";
export const kbdHint = `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;
export const borderDefault = "border-slate-200 dark:border-slate-800";
export const borderStrong = "border-slate-300 dark:border-slate-700";
export const focusVisibleRing = "focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400";
export const linkText = "text-sky-700 dark:text-sky-400";
export const linkHoverText = "hover:text-sky-900 dark:hover:text-sky-300";
export const dangerText = "text-rose-700 dark:text-rose-400";
export const calloutDangerBorder = "border-rose-200 dark:border-rose-900";
export const calloutDangerBg = "bg-rose-50 dark:bg-rose-950";
export const calloutWarningBorder = "border-amber-200 dark:border-amber-900";
export const calloutWarningBg = "bg-amber-50 dark:bg-amber-950";
export const calloutWarningText = "text-amber-950 dark:text-amber-300";
export const badgeBlocking = {
  bg: "bg-rose-100 dark:bg-rose-950",
  text: "text-rose-800 dark:text-rose-300",
};
export const statusConnecting = {
  bg: "bg-amber-100 dark:bg-amber-950",
  text: "text-amber-800 dark:text-amber-300",
};
/** A small circular status dot inline next to an agent's name (the agent state mark's filled
 * dot for a running session) — unlike the `STATUS_CONNECTED_BG`/`STATUS_OFFLINE_BG` pill
 * palettes of `classes.ts`, it has no label text of its own, so it uses a bolder, more saturated
 * fill to stay legible at that size. Purely decorative — no contrast check applies. */
export const liveDotBg = "bg-emerald-500 dark:bg-emerald-400";
/** The agent state mark's rings (`features/refs/AgentName.tsx`), beside `liveDotBg`'s filled dot
 * for a running session: a session that has missed its heartbeats without ending is a solid
 * amber ring, one the registry no longer lists a hollow slate ring, and one whose state is
 * unknown (no successful registry read yet, or the latest failed) a dashed neutral ring. Filled,
 * amber, hollow and dashed are the distinction that does not rest on colour alone.
 * Decorative — the mark's own `aria-label` names the state, so no contrast check applies. */
export const stalledDotBorder = "border-amber-500 dark:border-amber-400";
export const offlineDotBorder = "border-slate-400 dark:border-slate-600";
export const unknownDotBorder = "border-slate-400 dark:border-slate-500";
/** The primary submit/CTA button: one fixed background+text color pair in both schemes
 * (white on sky-700, unchanged from light to dark — `bg-sky-600` with white text measured
 * 4.02:1, under AA; `sky-700` measures 5.86:1). Owns its own text color so no caller writes a
 * raw `text-white` literal. */
export const primaryButtonBg = "bg-sky-700 text-white";
export const primaryButtonHoverBg = "hover:bg-sky-800";
/** The primary hover while the button can be pressed: `not-refused:` (`styles.css`) skips a
 *  `disabled` button and one refused with `aria-disabled` alike. */
export const primaryButtonEnabledHoverBg = "not-refused:hover:bg-sky-800";
export const secondaryButtonText = "text-slate-700 dark:text-slate-300";
export const secondaryButtonBorder = "border-slate-300 dark:border-slate-700";
export const secondaryButtonHoverBorder =
  "not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400";
/** A secondary button that cannot be pressed, `disabled` or refused (`refused:`, `styles.css`). */
export const secondaryButtonDisabledText = "refused:text-slate-400 dark:refused:text-slate-600";
/** An input/select/textarea's resting border and background, plus its focus border. Pass
 * `recessed: true` when the control sits inside a `SURFACE` card (its dark background needs to
 * read one level below the card, i.e. `SURFACE_RECESSED`); leave it `false` when the control
 * sits directly on `CANVAS` (its dark background reads one level above the canvas, i.e. plain
 * `SURFACE`). Both branches are complete literal strings — Tailwind's scanner sees both,
 * regardless of which one is returned at runtime. */
export function inputClasses(recessed: boolean): string {
  return recessed
    ? "border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-950 dark:focus:border-sky-400"
    : "border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-900 dark:focus:border-sky-400";
}
/** The compact secondary button the Architecture lists, the component picker and the broadcast
 *  Sends strip share: a 44 px tap target through phone widths, 28 px from `sm`. */
export const secondaryButtonCompact = `min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`;
export const railBg = "bg-slate-950";
export const railBorder = "border-slate-800";
export const railHoverBg = "hover:bg-slate-800";
export const railText = "text-slate-100";
export const railSecondaryText = "text-slate-300";
export const railMutedText = "text-slate-400";
export const railAccentText = "text-sky-300";
export const railAccentHoverText = "hover:text-sky-200";
export const railHoverText = "hover:text-slate-100";
export const railActiveBg = "bg-slate-800";
export const railFocusOverlayBg = "focus:bg-slate-950";
export const railFocusOverlayText = "focus:text-slate-100";
export const railDangerText = "text-rose-300";
export const railBadgeBg = "bg-sky-500";
export const railBadgeText = "text-slate-950";
/** The Inbox count uses amber because it means the viewer, not merely any agent, must act. */
export const railNeedsYouBadgeBg = "bg-amber-500";
export const railNeedsYouBadgeText = "text-slate-950";
export const pageInkText = "text-page-ink";
export const pageMutedText = "text-page-muted";
export const pageQuietText = "text-page-quiet";
export const pageLineBorder = "border-page-line";
/** The header's one filled control, Approve, filled with the page's ink as the composer's Send
 *  is (the prototype's own palette: the issue page draws no application blue). */
export const pageInkFill = "bg-page-ink text-page-paper";
/** The children's progress as a row of segments, one per child (the prototype's `.segs`): done
 *  in the passed green, in progress in the in-progress blue, the rest in the hairline grey. The
 *  two counts beside it say the same in words, so the bar is decoration. */
export const pageSegmentDoneBg = "bg-page-passed";
export const backdrop50 = "bg-slate-950/50";
/** A layout-preserving invisible border: reserves the border's width so a hover/focus state
 * that adds a real border doesn't shift the element, or so a loading skeleton matches its live
 * counterpart's box size exactly. Transparent has no color to differ between schemes — no
 * `dark:` variant, no contrast check. */
export const borderTransparent = "border-transparent";
/** Hides text while reserving its layout space, e.g. a skeleton placeholder revealing its own
 * pulsing background instead of the real (not-yet-loaded) text. Same reasoning as
 * `borderTransparent`. */
export const textTransparent = "text-transparent";
export const card = "border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900";
export const canvasText = "bg-slate-50 dark:bg-slate-950 text-slate-900 dark:text-slate-100";
