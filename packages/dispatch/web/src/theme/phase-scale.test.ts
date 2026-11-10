// Adjacent Legion phases must look different in a thin bar (dispatch://INFRA-979): each pair of
// phases next to each other in the bars' order differs by a CIEDE2000 ΔE of 20 or more, in the
// light scheme and in the dark one. Text contrast on each phase is the theme's own check
// (text-on-background.test.ts).
import { expect, test } from "bun:test";

import { PHASE_ORDER } from "../features/project/PhaseHoursBar";
import { legionPhaseScale } from "./classes";

type Lab = readonly [l: number, a: number, b: number];

function lab(hex: string): Lab {
  const linear = [0, 2, 4].map((at) => {
    const c = Number.parseInt(hex.slice(at, at + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  const [r = 0, g = 0, b = 0] = linear;
  const x = (0.4124 * r + 0.3576 * g + 0.1805 * b) / 0.95047;
  const y = 0.2126 * r + 0.7152 * g + 0.0722 * b;
  const z = (0.0193 * r + 0.1192 * g + 0.9505 * b) / 1.08883;
  const f = (t: number) => (t > 0.008856 ? Math.cbrt(t) : 7.787 * t + 16 / 116);
  return [116 * f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))];
}

const rad = (deg: number) => (deg * Math.PI) / 180;
const deg = (r: number) => ((r * 180) / Math.PI + 360) % 360;

/** CIEDE2000 colour difference (Sharma, Wu and Dalal 2005). */
function deltaE2000(one: Lab, two: Lab): number {
  const [l1, a1, b1] = one;
  const [l2, a2, b2] = two;
  const cBar = (Math.hypot(a1, b1) + Math.hypot(a2, b2)) / 2;
  const g = 0.5 * (1 - Math.sqrt(cBar ** 7 / (cBar ** 7 + 25 ** 7)));
  const a1p = (1 + g) * a1;
  const a2p = (1 + g) * a2;
  const c1p = Math.hypot(a1p, b1);
  const c2p = Math.hypot(a2p, b2);
  const h1p = deg(Math.atan2(b1, a1p));
  const h2p = deg(Math.atan2(b2, a2p));
  let dh = h2p - h1p;
  if (c1p * c2p === 0) dh = 0;
  else if (dh > 180) dh -= 360;
  else if (dh < -180) dh += 360;
  const dL = l2 - l1;
  const dC = c2p - c1p;
  const dH = 2 * Math.sqrt(c1p * c2p) * Math.sin(rad(dh / 2));
  const lBar = (l1 + l2) / 2;
  const cBarP = (c1p + c2p) / 2;
  let hBar = h1p + h2p;
  if (c1p * c2p !== 0) {
    if (Math.abs(h1p - h2p) <= 180) hBar = (h1p + h2p) / 2;
    else hBar = h1p + h2p < 360 ? (h1p + h2p + 360) / 2 : (h1p + h2p - 360) / 2;
  }
  const t =
    1 -
    0.17 * Math.cos(rad(hBar - 30)) +
    0.24 * Math.cos(rad(2 * hBar)) +
    0.32 * Math.cos(rad(3 * hBar + 6)) -
    0.2 * Math.cos(rad(4 * hBar - 63));
  const dTheta = 30 * Math.exp(-(((hBar - 275) / 25) ** 2));
  const rC = 2 * Math.sqrt(cBarP ** 7 / (cBarP ** 7 + 25 ** 7));
  const sL = 1 + (0.015 * (lBar - 50) ** 2) / Math.sqrt(20 + (lBar - 50) ** 2);
  const sC = 1 + 0.045 * cBarP;
  const sH = 1 + 0.015 * cBarP * t;
  const rT = -Math.sin(rad(2 * dTheta)) * rC;
  return Math.sqrt((dL / sL) ** 2 + (dC / sC) ** 2 + (dH / sH) ** 2 + rT * (dC / sC) * (dH / sH));
}

/** A phase's fill in one scheme, read from its classes. */
function fill(phase: string, scheme: "light" | "dark"): string {
  const classes = legionPhaseScale[phase] ?? "";
  const pattern =
    scheme === "light" ? /(?:^| )bg-\[#([0-9a-f]{6})\]/ : /dark:bg-\[#([0-9a-f]{6})\]/;
  const hex = pattern.exec(classes)?.[1];
  if (hex === undefined) throw new Error(`no ${scheme} fill for ${phase}`);
  return hex;
}

test("the CIEDE2000 formula matches Sharma's published pair", () => {
  expect(deltaE2000([50, 2.6772, -79.7751], [50, 0, -82.7485])).toBeCloseTo(2.0425, 3);
});

for (const scheme of ["light", "dark"] as const) {
  test(`each pair of adjacent phases differs by ΔE ≥ 20 in the ${scheme} scheme`, () => {
    const pairs = PHASE_ORDER.slice(1).map((phase, index) => {
      const before = PHASE_ORDER[index]?.id ?? "";
      const difference = deltaE2000(lab(fill(before, scheme)), lab(fill(phase.id, scheme)));
      return { pair: `${before}/${phase.id}`, difference: Math.round(difference * 10) / 10 };
    });
    expect(pairs.filter((one) => one.difference < 20)).toEqual([]);
  });
}
