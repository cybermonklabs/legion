import type { Oklch } from "./contrast";

/**
 * Every Tailwind color shade this app renders, named. OKLCH values are copied from
 * `tailwindcss`'s default theme (`node_modules/tailwindcss/theme.css`) — Tailwind v4 defines its
 * default palette in OKLCH, not hex, and several hues moved meaningfully from Tailwind v3's
 * published hex constants (`sky-700` renders as `#0069a8` now, not v3's `#0369a1`), so this is
 * sourced from the real values rather than memorized hex.
 *
 * `classes.ts` is the only other module allowed to import this one for anything other than
 * tests: it is the sole place color *roles* (surface, text-primary, danger, ...) are assigned to
 * a swatch, and the sole place a component-facing `dark:`-paired className string is built.
 * Nothing outside `theme/` should import `palette.ts` directly or write a Tailwind color utility
 * literal — see `no-raw-colors.test.ts`, which enforces that mechanically.
 */
export interface Swatch {
  readonly name: string;
  readonly oklch: Oklch;
}

function swatch(name: string, l: number, c: number, h: number): Swatch {
  return { name, oklch: { l, c, h } };
}

export const WHITE = swatch("white", 1, 0, 0);

export const SLATE_50 = swatch("slate-50", 0.984, 0.003, 247.858);
export const SLATE_100 = swatch("slate-100", 0.968, 0.007, 247.896);
export const SLATE_200 = swatch("slate-200", 0.929, 0.013, 255.508);
export const SLATE_300 = swatch("slate-300", 0.869, 0.022, 252.894);
export const SLATE_400 = swatch("slate-400", 0.704, 0.04, 256.788);
export const SLATE_500 = swatch("slate-500", 0.554, 0.046, 257.417);
export const SLATE_600 = swatch("slate-600", 0.446, 0.043, 257.281);
export const SLATE_700 = swatch("slate-700", 0.372, 0.044, 257.287);
export const SLATE_800 = swatch("slate-800", 0.279, 0.041, 260.031);
export const SLATE_900 = swatch("slate-900", 0.208, 0.042, 265.755);
export const SLATE_950 = swatch("slate-950", 0.129, 0.042, 264.695);

export const SKY_50 = swatch("sky-50", 0.977, 0.013, 236.62);
export const SKY_100 = swatch("sky-100", 0.951, 0.026, 236.824);
export const SKY_200 = swatch("sky-200", 0.901, 0.058, 230.902);
export const SKY_300 = swatch("sky-300", 0.828, 0.111, 230.318);
export const SKY_400 = swatch("sky-400", 0.746, 0.16, 232.661);
export const SKY_500 = swatch("sky-500", 0.685, 0.169, 237.323);
export const SKY_600 = swatch("sky-600", 0.588, 0.158, 241.966);
export const SKY_700 = swatch("sky-700", 0.5, 0.134, 242.749);
export const SKY_800 = swatch("sky-800", 0.443, 0.11, 240.79);
export const SKY_900 = swatch("sky-900", 0.391, 0.09, 240.876);
export const SKY_950 = swatch("sky-950", 0.293, 0.066, 243.157);

export const ROSE_50 = swatch("rose-50", 0.969, 0.015, 12.422);
export const ROSE_100 = swatch("rose-100", 0.941, 0.03, 12.58);
export const ROSE_200 = swatch("rose-200", 0.892, 0.058, 10.001);
export const ROSE_300 = swatch("rose-300", 0.81, 0.117, 11.638);
export const ROSE_400 = swatch("rose-400", 0.712, 0.194, 13.428);
export const ROSE_700 = swatch("rose-700", 0.514, 0.222, 16.935);
export const ROSE_800 = swatch("rose-800", 0.455, 0.188, 13.697);
export const ROSE_900 = swatch("rose-900", 0.41, 0.159, 10.272);
export const ROSE_950 = swatch("rose-950", 0.271, 0.105, 12.094);

export const AMBER_50 = swatch("amber-50", 0.987, 0.022, 95.277);
export const AMBER_100 = swatch("amber-100", 0.962, 0.059, 95.617);
export const AMBER_200 = swatch("amber-200", 0.924, 0.12, 95.746);
export const AMBER_300 = swatch("amber-300", 0.879, 0.169, 91.605);
export const AMBER_400 = swatch("amber-400", 0.828, 0.189, 84.429);
export const AMBER_500 = swatch("amber-500", 0.769, 0.188, 70.08);
export const AMBER_700 = swatch("amber-700", 0.555, 0.163, 48.998);
export const AMBER_800 = swatch("amber-800", 0.473, 0.137, 46.201);
export const AMBER_900 = swatch("amber-900", 0.414, 0.112, 45.904);
export const AMBER_950 = swatch("amber-950", 0.279, 0.077, 45.635);

export const EMERALD_50 = swatch("emerald-50", 0.979, 0.021, 166.113);
export const EMERALD_100 = swatch("emerald-100", 0.95, 0.052, 163.051);
export const EMERALD_200 = swatch("emerald-200", 0.905, 0.093, 164.15);
export const EMERALD_300 = swatch("emerald-300", 0.845, 0.143, 164.978);
export const EMERALD_400 = swatch("emerald-400", 0.765, 0.177, 163.223);
export const EMERALD_600 = swatch("emerald-600", 0.596, 0.145, 163.225);
export const EMERALD_700 = swatch("emerald-700", 0.508, 0.118, 165.612);
export const EMERALD_800 = swatch("emerald-800", 0.432, 0.095, 166.913);
export const EMERALD_900 = swatch("emerald-900", 0.378, 0.077, 168.94);
export const EMERALD_950 = swatch("emerald-950", 0.262, 0.051, 172.552);

export const STONE_300 = swatch("stone-300", 0.869, 0.005, 56.366);
export const STONE_800 = swatch("stone-800", 0.268, 0.007, 34.298);

export const BLUE_300 = swatch("blue-300", 0.809, 0.105, 251.813);
// biome-ignore lint/suspicious/noApproximativeNumericConstant: Tailwind's own lightness, not √½.
export const BLUE_400 = swatch("blue-400", 0.707, 0.165, 254.624);
export const BLUE_600 = swatch("blue-600", 0.546, 0.245, 262.881);
export const BLUE_700 = swatch("blue-700", 0.488, 0.243, 264.376);
export const BLUE_950 = swatch("blue-950", 0.282, 0.091, 267.935);
export const VIOLET_400 = swatch("violet-400", 0.702, 0.183, 293.541);
export const VIOLET_950 = swatch("violet-950", 0.283, 0.141, 291.089);
export const TEAL_400 = swatch("teal-400", 0.777, 0.152, 181.912);
export const TEAL_950 = swatch("teal-950", 0.277, 0.046, 192.524);
export const INDIGO_400 = swatch("indigo-400", 0.673, 0.182, 276.935);
export const INDIGO_950 = swatch("indigo-950", 0.257, 0.09, 281.288);
export const PINK_400 = swatch("pink-400", 0.718, 0.202, 349.761);
export const PINK_700 = swatch("pink-700", 0.525, 0.223, 3.958);
export const PINK_950 = swatch("pink-950", 0.284, 0.109, 3.907);
export const FUCHSIA_400 = swatch("fuchsia-400", 0.74, 0.238, 322.16);
export const FUCHSIA_950 = swatch("fuchsia-950", 0.293, 0.136, 325.661);
export const ORANGE_300 = swatch("orange-300", 0.837, 0.128, 66.29);
export const ORANGE_400 = swatch("orange-400", 0.75, 0.183, 55.934);
export const ORANGE_700 = swatch("orange-700", 0.553, 0.195, 38.402);
export const ORANGE_950 = swatch("orange-950", 0.266, 0.079, 36.259);
export const GREEN_400 = swatch("green-400", 0.792, 0.209, 151.711);
export const RED_400 = swatch("red-400", 0.704, 0.191, 22.216);
// biome-ignore lint/suspicious/noApproximativeNumericConstant: Tailwind's own lightness, not √½.
export const GRAY_400 = swatch("gray-400", 0.707, 0.022, 261.325);

/**
 * The issue page's own colours, each the value the approved prototype's stylesheet declares
 * (dispatch://INFRA-1196/artifact/issue-page-prototype-html, version 3: its `:root` tokens, and
 * the few literals its left pane, rail and waiting chip write), so the page is drawn in the
 * colours of the pictures its owner approved rather than in the nearest Tailwind shade. Each is
 * named for what it is on that page; its OKLCH triple renders to the prototype's hex exactly
 * (`styles-css-pin.test.ts` holds `styles.css` to it). The dark scheme draws Tailwind shades of
 * the same hue in their place (`styles.css`, `classes.ts`'s `PAGE_COLORS`).
 */
export const PAGE_PAPER = swatch("page-paper", 1, 0, 0);
export const PAGE_INK = swatch("page-ink", 0.2161, 0.0061, 56.043);
/** A node's words, the prototype's `.body` (`#292524`): one step off the ink, which the
 *  reader's own words keep. */
export const PAGE_WORDS = swatch("page-words", 0.2685, 0.0063, 34.298);
export const PAGE_MUTED = swatch("page-muted", 0.4444, 0.0096, 73.639);
export const PAGE_QUIET = swatch("page-quiet", 0.5534, 0.0116, 58.071);
export const PAGE_LINE = swatch("page-line", 0.9378, 0.0046, 78.297);
export const PAGE_HAIR = swatch("page-hair", 0.8837, 0.0134, 82.401);
export const PAGE_PANE = swatch("page-pane", 0.9848, 0.0013, 106.423);
export const PAGE_MARK = swatch("page-mark", 0.9468, 0.0046, 78.298);
export const PAGE_GUIDE = swatch("page-guide", 0.9232, 0.0026, 48.717);
/** The bullet of an agent's words and of a node of no kind the prototype names (`#A8A29E`). */
export const PAGE_BULLET = swatch("page-bullet", 0.7161, 0.0091, 56.259);
export const PAGE_QUESTION = swatch("page-question", 0.5461, 0.2152, 262.881);
export const PAGE_QUESTION_TINT = swatch("page-question-tint", 0.9705, 0.0142, 254.604);
export const PAGE_PERSON_TINT = swatch("page-person-tint", 0.9513, 0.0217, 83.264);
export const PAGE_DECISION = swatch("page-decision", 0.5413, 0.2466, 293.009);
export const PAGE_DECISION_TINT = swatch("page-decision-tint", 0.9691, 0.0161, 293.756);
export const PAGE_STATEMENT = swatch("page-statement", 0.5109, 0.0861, 186.391);
export const PAGE_STATEMENT_TINT = swatch("page-statement-tint", 0.9836, 0.0142, 180.72);
export const PAGE_CHECK = swatch("page-check", 0.4568, 0.2146, 277.023);
export const PAGE_CHECK_TINT = swatch("page-check-tint", 0.9619, 0.0179, 272.314);
export const PAGE_STEP = swatch("page-step", 0.5553, 0.1455, 48.998);
export const PAGE_STEP_TINT = swatch("page-step-tint", 0.9869, 0.0214, 95.277);
export const PAGE_CHILD = swatch("page-child", 0.5916, 0.218, 0.584);
export const PAGE_CHILD_TINT = swatch("page-child-tint", 0.9714, 0.0141, 343.198);
export const PAGE_LENS = swatch("page-lens", 0.518, 0.2258, 323.949);
export const PAGE_LENS_TINT = swatch("page-lens-tint", 0.9773, 0.0173, 320.058);
export const PAGE_AGENT = swatch("page-agent", 0.5544, 0.0407, 257.417);
export const PAGE_ISSUE = swatch("page-issue", 0.3741, 0.0087, 67.558);
export const PAGE_WAITING = swatch("page-waiting", 0.6461, 0.1943, 41.116);
export const PAGE_WAITING_TINT = swatch("page-waiting-tint", 0.9796, 0.0158, 73.684);
export const PAGE_WAITING_TEXT = swatch("page-waiting-text", 0.5534, 0.1739, 38.402);
export const PAGE_PASSED = swatch("page-passed", 0.5273, 0.1371, 150.069);
export const PAGE_FAILED = swatch("page-failed", 0.5054, 0.1905, 27.518);
export const PAGE_NOT_RUN = swatch("page-not-run", 0.551, 0.0234, 264.364);
export const PAGE_IN_PROGRESS = swatch("page-in-progress", 0.4882, 0.2172, 264.376);
/** The icon rail the issue page draws in place of the shell's navigation column: the
 *  prototype's `#rail`, a fixed dark surface in both schemes like the shell's own rail. Its
 *  active entry's ground is the prototype's white at 8 % over the rail, composited. */
export const ICON_RAIL = swatch("icon-rail", 0.1889, 0.0369, 264.087);
export const ICON_RAIL_TEXT = swatch("icon-rail-text", 0.869, 0.0198, 252.894);
export const ICON_RAIL_ACCENT = swatch("icon-rail-accent", 0.7535, 0.139, 232.661);
export const ICON_RAIL_ACTIVE = swatch("icon-rail-active", 0.2695, 0.0316, 266.051);
