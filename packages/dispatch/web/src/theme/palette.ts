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

// IBM Carbon's data-visualization colours (https://carbondesignsystem.com/data-visualization/color-palettes/):
// its categorical palette's hues and its gray ramp, which the Legion phase scale and the issue
// status scale use (dispatch://INFRA-979). OKLCH converted from Carbon's published hex values.
export const CARBON_PURPLE_70 = swatch("carbon-purple-70", 0.468, 0.219, 295.08); // #6929c4
export const CARBON_PURPLE_50 = swatch("carbon-purple-50", 0.663, 0.208, 298.001); // #a56eff
export const CARBON_PURPLE_60 = swatch("carbon-purple-60", 0.574, 0.258, 294.891); // #8a3ffc
export const CARBON_PURPLE_30 = swatch("carbon-purple-30", 0.836, 0.097, 301.177); // #d4bbff
export const CARBON_CYAN_50 = swatch("carbon-cyan-50", 0.641, 0.162, 246.51); // #1192e8
export const CARBON_CYAN_40 = swatch("carbon-cyan-40", 0.729, 0.155, 241.664); // #33b1ff
export const CARBON_TEAL_70 = swatch("carbon-teal-70", 0.433, 0.074, 194.769); // #005d5d
export const CARBON_TEAL_50 = swatch("carbon-teal-50", 0.629, 0.108, 192.369); // #009d9a
export const CARBON_TEAL_60 = swatch("carbon-teal-60", 0.532, 0.092, 190.843); // #007d79
export const CARBON_TEAL_40 = swatch("carbon-teal-40", 0.722, 0.123, 192.748); // #08bdba
export const CARBON_MAGENTA_70 = swatch("carbon-magenta-70", 0.465, 0.172, 1.339); // #9f1853
export const CARBON_MAGENTA_40 = swatch("carbon-magenta-40", 0.756, 0.167, 354.525); // #ff7eb6
export const CARBON_BLUE_80 = swatch("carbon-blue-80", 0.363, 0.184, 263.056); // #002d9c
export const CARBON_BLUE_50 = swatch("carbon-blue-50", 0.647, 0.189, 260.581); // #4589ff
export const CARBON_YELLOW_50 = swatch("carbon-yellow-50", 0.645, 0.132, 85.25); // #b28600
export const CARBON_YELLOW_40 = swatch("carbon-yellow-40", 0.734, 0.149, 86.457); // #d2a106
export const CARBON_GREEN_60 = swatch("carbon-green-60", 0.527, 0.141, 148.295); // #198038
export const CARBON_GREEN_30 = swatch("carbon-green-30", 0.81, 0.152, 150.442); // #6fdc8c
export const CARBON_GRAY_10 = swatch("carbon-gray-10", 0.967, 0.0, 89.876); // #f4f4f4
export const CARBON_GRAY_20 = swatch("carbon-gray-20", 0.907, 0.0, 89.876); // #e0e0e0
export const CARBON_GRAY_30 = swatch("carbon-gray-30", 0.827, 0.0, 89.876); // #c6c6c6
export const CARBON_GRAY_40 = swatch("carbon-gray-40", 0.732, 0.0, 89.876); // #a8a8a8
export const CARBON_GRAY_50 = swatch("carbon-gray-50", 0.643, 0.0, 89.876); // #8d8d8d
export const CARBON_GRAY_60 = swatch("carbon-gray-60", 0.542, 0.0, 89.876); // #6f6f6f
export const CARBON_GRAY_70 = swatch("carbon-gray-70", 0.439, 0.0, 89.876); // #525252
export const CARBON_GRAY_80 = swatch("carbon-gray-80", 0.345, 0.0, 89.876); // #393939
export const CARBON_GRAY_100 = swatch("carbon-gray-100", 0.2, 0.0, 89.876); // #161616
