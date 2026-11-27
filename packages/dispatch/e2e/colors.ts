/** Computed colours as the browser reports them, read as 8-bit sRGB, and the WCAG 2.1 contrast
 *  of two of them. Shared by `theme.stub.e2e.ts`, which holds each element to the exact swatch
 *  `theme/classes.ts` names for it, and by `keyboard-complete.ts`, whose rows hold the text of each
 *  menu and list to a contrast ratio.
 *
 *  Chromium serializes `getComputedStyle` colors in the color space they were declared in —
 *  Tailwind v4's default palette is `oklch()`, so `backgroundColor`/`color`/`borderBottomColor`
 *  come back as e.g. `oklch(0.208 0.042 265.755)`, not `rgb(...)`, and a color an opacity modifier
 *  writes (a `color-mix()` in oklab) comes back as `oklab(... / 0.4)`. Rather
 *  than pin that literal string (brittle against browser/Tailwind version formatting changes),
 *  this converts the actual computed color through the same OKLCH→sRGB math `palette.test.ts`
 *  uses to verify contrast (`web/src/theme/contrast.ts`, which this tree does not change). */
import { oklchToSrgb8, type Rgb8 } from "../web/src/theme/contrast";

/** One computed color: its 8-bit sRGB channels and its alpha, from 0 (transparent) to 1. */
export interface ComputedColor {
  readonly rgb: Rgb8;
  readonly alpha: number;
}

/** A color function the browser writes a computed color with, and what is inside its brackets. */
const COLOR_FUNCTION = /^(rgba?|oklch|oklab|color)\((.*)\)$/;

export function toRgbString([r, g, b]: Rgb8): string {
  return `rgb(${r}, ${g}, ${b})`;
}

/** One number of a computed color. `none`, a component the color has no value for (the hue of
 *  a gray), is 0, as CSS reads it. A percentage is refused here, since no browser writes a
 *  computed channel as one; only an alpha may be one (`alphaOf`). */
function channel(token: string, value: string): number {
  const number = token === "none" ? 0 : Number(token);
  if (!Number.isFinite(number)) {
    throw new Error(`could not parse a css color from "${value}"`);
  }
  return number;
}

function alphaOf(token: string | undefined, value: string): number {
  if (token === undefined) return 1;
  return token.endsWith("%") ? channel(token.slice(0, -1), value) / 100 : channel(token, value);
}

/** An 8-bit channel from a 0-255 value the browser may write with a fraction. */
function byte(value: number): number {
  return Math.min(255, Math.max(0, Math.round(value)));
}

/** Parses a computed color (`rgb(...)`/`rgba(...)`, `oklch(...)`, `oklab(...)` or
 *  `color(srgb ...)`, in the legacy comma form or the space form with `/ alpha`) into its 8-bit
 *  sRGB channels and its alpha. */
export function parseComputedColor(value: string): ComputedColor {
  const match = COLOR_FUNCTION.exec(value.trim());
  if (match === null) {
    throw new Error(`could not parse a css color from "${value}"`);
  }
  const [, name, inside] = match;
  const [channelText, slashAlpha] = inside.split("/");
  const tokens = channelText.split(/[\s,]+/).filter((token) => token !== "");
  if (name === "color") {
    if (tokens[0] !== "srgb" || tokens.length !== 4) {
      throw new Error(`could not parse a css color from "${value}"`);
    }
    const [r, g, b] = tokens.slice(1).map((token) => byte(channel(token, value) * 255));
    return { alpha: alphaOf(slashAlpha?.trim(), value), rgb: [r, g, b] };
  }
  // The legacy form writes the alpha as a fourth comma-separated number.
  if (tokens.length !== 3 && !(tokens.length === 4 && slashAlpha === undefined)) {
    throw new Error(`could not parse a css color from "${value}"`);
  }
  const alpha = alphaOf(slashAlpha?.trim() ?? tokens[3], value);
  const [first, second, third] = tokens.slice(0, 3).map((token) => channel(token, value));
  if (name === "oklch") {
    return { alpha, rgb: oklchToSrgb8({ c: second, h: third, l: first }) };
  }
  if (name === "oklab") {
    return {
      alpha,
      rgb: oklchToSrgb8({
        c: Math.hypot(second, third),
        h: (Math.atan2(third, second) * 180) / Math.PI,
        l: first,
      }),
    };
  }
  return { alpha, rgb: [byte(first), byte(second), byte(third)] };
}

/** Normalizes a computed color string to a canonical `rgb(r, g, b)` 8-bit triple, dropping
 *  alpha — none of `theme.stub.e2e.ts`'s assertions target a translucent element. */
export function normalizeComputedColor(value: string): string {
  return toRgbString(parseComputedColor(value).rgb);
}

/** `top` painted over the opaque `bottom`, as the browser composites one color over another
 *  (source-over, in sRGB): what a reader sees where a translucent color lies on another. */
export function composite(top: ComputedColor, bottom: Rgb8): Rgb8 {
  const [r, g, b] = top.rgb.map((value, index) =>
    byte(value * top.alpha + bottom[index] * (1 - top.alpha))
  );
  return [r, g, b];
}

/** The WCAG 2.1 relative luminance of an 8-bit sRGB color
 *  (https://www.w3.org/TR/WCAG21/#dfn-relative-luminance). The threshold is the corrected
 *  0.04045 that `web/src/theme/contrast.ts` explains; the published 0.03928 reads every 8-bit
 *  channel the same way, since no n/255 lies between the two. */
export function relativeLuminance([r, g, b]: Rgb8): number {
  const linearize = (channel8: number) => {
    const value = channel8 / 255;
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * linearize(r) + 0.7152 * linearize(g) + 0.0722 * linearize(b);
}

/** The WCAG 2.1 contrast ratio of two 8-bit sRGB colors
 *  (https://www.w3.org/TR/WCAG21/#dfn-contrast-ratio), from 1 to 21. */
export function contrastRatio(first: Rgb8, second: Rgb8): number {
  const a = relativeLuminance(first);
  const b = relativeLuminance(second);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}
